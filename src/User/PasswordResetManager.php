<?php
declare(strict_types=1);

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
    public function __construct(
        private readonly UserRepository $userRepo,
        private readonly PasswordResetTokenRepository $tokenRepo,
        private readonly EntityManagerInterface $em,
        private readonly MailerInterface $mailer,
        private readonly RouterInterface $router,
        private readonly string $fromAddress
    ) {

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
