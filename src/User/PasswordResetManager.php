<?php


namespace App\User;

use App\Repository\PasswordResetTokenRepository;
use Doctrine\ORM\EntityManagerInterface;
use App\Entity\{PasswordResetToken, User};
use App\Repository\UserRepository;
use App\User\Exception\UserNotFoundException;
use Symfony\Component\Mailer\MailerInterface;
use Symfony\Component\Mime\Email;
use Symfony\Component\Routing\{Generator\UrlGeneratorInterface, RouterInterface};

class PasswordResetManager
{
    /** @var UserRepository */
    private $userRepo;

    /** @var PasswordResetTokenRepository */
    private $tokenRepo;

    /** @var EntityManagerInterface */
    private $em;

    /** @var MailerInterface */
    private $mailer;

    /** @var RouterInterface */
    private $router;

    /** @var string */
    private $fromAddress;

    public function __construct(
        UserRepository $userRepo,
        PasswordResetTokenRepository $tokenRepo,
        EntityManagerInterface $em,
        MailerInterface $mailer,
        RouterInterface $router,
        string $fromAddress
    ) {
        $this->userRepo = $userRepo;
        $this->tokenRepo = $tokenRepo;
        $this->em = $em;
        $this->mailer = $mailer;
        $this->router = $router;
        $this->fromAddress = $fromAddress;
    }

    public function sendResetLink(string $address): void
    {
        /** @var User $user */
        if (null === $user = $this->userRepo->findOneBy(['email' => $address])) {
            throw new UserNotFoundException();
        }

        // @todo add limits
        $token = new PasswordResetToken($user);
        $this->tokenRepo->add($token);
        $this->em->flush();

        $link = $this->router->generate('user_reset', ['code' => $token->getCode()], UrlGeneratorInterface::ABSOLUTE_URL);

        $mail =  (new Email())
            ->from($this->fromAddress)
            ->to($user->getEmail())
            ->subject('Password reset')
            ->text(<<<MAIL
                Here is your password reset link:
                
                $link
                MAIL
            )
        ;

        $this->mailer->send($mail);
    }
}
