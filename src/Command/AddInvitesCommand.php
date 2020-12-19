<?php

namespace App\Command;

use App\Repository\UserRepository;
use App\User\InviteManager;
use Doctrine\ORM\EntityManagerInterface;
use Symfony\Component\Console\Command\Command;
use Symfony\Component\Console\Input\{InputArgument, InputInterface};
use Symfony\Component\Console\Output\OutputInterface;

class AddInvitesCommand extends Command
{
    /** @var EntityManagerInterface */
    private $em;

    /** @var UserRepository */
    private $userRepo;

    /** @var InviteManager */
    private $inviteManager;

    public function __construct(EntityManagerInterface $em, UserRepository $userRepo, InviteManager $inviteManager)
    {
        parent::__construct();

        $this->em = $em;
        $this->userRepo = $userRepo;
        $this->inviteManager = $inviteManager;
    }

    protected function configure()
    {
        $this
            ->setName('invite:add')
            ->addArgument('username', InputArgument::REQUIRED, 'Username')
            ->addArgument('number', InputArgument::REQUIRED, 'Number of invites')
        ;
    }

    protected function execute(InputInterface $input, OutputInterface $output)
    {
        $username = $input->getArgument('username');
        $number = $input->getArgument('number');

        if (null === $user = $this->userRepo->findOneBy(['username' => $username])) {
            $output->writeln('<error>User not found.</error>');

            return 1;
        }

        $this->inviteManager->createInvitesForUser($user, $number);

        $this->em->flush();

        $output->writeln(sprintf('<info>%d invites added to \'%s\'.</info>', $number, $user->getUsername()));

        return 0;
    }
}
