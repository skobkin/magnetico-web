<?php

namespace App\Command;

use App\Entity\Invite;
use App\Repository\{InviteRepository, UserRepository};
use App\User\UserManager;
use Doctrine\ORM\EntityManagerInterface;
use Symfony\Component\Console\Command\Command;
use Symfony\Component\Console\Helper\QuestionHelper;
use Symfony\Component\Console\Input\{InputArgument, InputInterface, InputOption};
use Symfony\Component\Console\Output\OutputInterface;
use Symfony\Component\Console\Question\Question;

class AddUserCommand extends Command
{
    /** @var EntityManagerInterface */
    private $em;

    /** @var UserManager */
    private $userManager;

    /** @var UserRepository */
    private $userRepo;

    /** @var InviteRepository */
    private $inviteRepo;

    public function __construct(EntityManagerInterface $em, UserManager $userManager, UserRepository $userRepo, InviteRepository $inviterepo)
    {
        parent::__construct();

        $this->em = $em;
        $this->userManager = $userManager;
        $this->userRepo = $userRepo;
        $this->inviteRepo = $inviterepo;
    }

    protected function configure()
    {
        $this
            ->setName('user:add')
            ->addArgument('username', InputArgument::REQUIRED, 'Username')
            ->addArgument('email', InputArgument::REQUIRED, 'Email')
            ->addArgument('password', InputArgument::OPTIONAL, 'Password', null)
            ->addOption('invites', 'i', InputOption::VALUE_OPTIONAL, 'Number of invites for user', 0)
            ->addOption('role', 'r', InputOption::VALUE_IS_ARRAY | InputOption::VALUE_OPTIONAL, 'Role to add to the user')
        ;
    }

    protected function execute(InputInterface $input, OutputInterface $output)
    {
        $username = $input->getArgument('username');
        $email = $input->getArgument('email');
        $password = $input->getArgument('password');
        $invites = (int) $input->getOption('invites');
        $roles = (array) $input->getOption('role');

        if (!$password) {
            /** @var QuestionHelper $questionHelper */
            $questionHelper = $this->getHelper('question');
            $question = new Question('Enter new user\'s password: ');
            $question->setHidden(true);
            $question->setHiddenFallback(false);

            $password = $questionHelper->ask($input, $output, $question);
        }

        if (!$password) {
            $output->writeln('User password cannot be empty.');

            return 1;
        }

        if ($roles) {
            $user = $this->userManager->createUser($username, $password, $email, $roles);
        } else {
            $user = $this->userManager->createUser($username, $password, $email);
        }


        $this->userRepo->add($user);

        if ($invites) {
            for ($i = 0; $i < $invites; $i++) {
                $invite = new Invite($user);
                $this->inviteRepo->add($invite);
            }
        }

        $this->em->flush();

        $output->writeln(sprintf('User \'%s\' registered, %d invites added.', $user->getUsername(), $invites));

        return 0;
    }

}